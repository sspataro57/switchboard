# Handoff to the kube session — the IMAP IDLE watcher becomes the live mail path (SWT-73)

**This one adds a resident workload; Salvador asked for it** ("I don't like rolling imap per 10
minutes. imap ingestion should subscribe so they get emails instantly") and decided the CronJob's
fate ("move it to every 2 hours to catch misses"). The design in one paragraph:

> A new `connector-google-watch` Deployment (1 replica) runs the connector's existing `--watch`
> mode: one IMAP IDLE connection per mailbox on INBOX, so a new mail runs the same pass the
> CronJob runs today within seconds, plus a full reconcile sweep every 10 minutes. It holds a
> singleton lock (a second replica stands by, never crashes), bounds every pass to 10 minutes,
> serves `/healthz` on :8092 for the kubelet, and refuses to start on a configuration that would
> capture nothing. The existing `connector-google` CronJob is moved from `*/10 * * * *` to
> `0 */2 * * *` as a safety net — **not suspended**. Rollback is two patches and no image.

Spec: `docs/tickets/imap-idle-watch_SPEC.md`. Runbook: `docs/runbooks/imap-mail-connector.md`
("Running it resident"). Image tag and digest are in the message that accompanies this file
(built from `main` after the merge).

## 0. No migration, no new column, no new tool

Nothing in the database changes. The lock is a session advisory lock (`0x5157_0010`), health is
an HTTP probe. The image is the usual one: `./cmd/connectors/...` already builds `google`.

## 1. Roll the image tag to every existing workload as usual

Nothing behaves differently yet: the new code runs only under `--watch`. Keep the pins
(`classify-promote --lane personal`, pipelined's `PIPELINE_STAGES`, `MS_OAUTH_CLIENT_ID` on
`connector-google`). Check no Send is in flight before replacing the dashboard pod.

## 2. The new Deployment: `connector-google-watch`

Same image, command `["/usr/local/bin/google", "--watch"]`, **1 replica, `strategy: Recreate`**
(a rolling update would run two watchers, both resolving the MSN credential — see the runbook's
connection note), no Service, no Ingress, `terminationGracePeriodSeconds: 120` (SIGTERM cancels
the context; an in-flight pass finishes or is cancelled cleanly; exit 0).

**Env — parity with the CronJob is a GATE, not advice.** The two processes take turns on the same
four mailboxes under the per-account lock, so drift in `CAPTURE_RULES_MODE`, `CAPTURE_RULES_SINCE`
or `MAIL_MAX_MESSAGE_BYTES` gives two behaviours on one mailbox depending on which process won,
invisible in the logs. Copy the CronJob's env verbatim (read 2026-09-22 15:20 EDT: `MQTT_BROKER`,
`CAPTURE_RULES_MODE=live`, `DATABASE_URL`, `OPS_TOKEN_KEY`, `MAIL_SOURCE=imap`,
`MS_OAUTH_CLIENT_ID`; no `CAPTURE_RULES_SINCE`, no `MAIL_MAX_MESSAGE_BYTES`) and add nothing but
the watch knobs:

| env | value | meaning |
|---|---|---|
| `DATABASE_URL` | same secret | required |
| `OPS_TOKEN_KEY` | same secret | without it the watcher cannot decrypt a single credential (exit 1 at start) |
| `MS_OAUTH_CLIENT_ID` | same value | the 2026-09-18 correction: "if it is ever deployed, it needs this variable too" — the MSN mailbox mints under it |
| `CAPTURE_RULES_MODE` | `live` | parity; `shadow` is accepted and PRINTED, and creates no tasks |
| `CAPTURE_RULES_SINCE` | unset (as on the CronJob) | parity; if ever set with `live`, it must be ≥ `2h` or the pod exits 1 |
| `MAIL_MAX_MESSAGE_BYTES` | unset (as on the CronJob) | parity |
| `MQTT_BROKER` | `tcp://192.168.50.45:1883` | unset, `AnnounceCaptured` skips and instant mail does not become instant tasks |
| `MAIL_SOURCE` | `imap` (harmless) | IGNORED in watch mode — watch is IMAP-only by construction |
| `MAIL_RECONCILE_INTERVAL` | `10m` (default) | the sweep |
| `MAIL_IDLE_REFRESH` | `25m` (default) | IDLE re-issue, under RFC 2177's 29m |
| `MAIL_PASS_TIMEOUT` | `10m` (default) | bound on every pass |
| `MAIL_WATCH_HEALTH_ADDR` | `:8092` (default) | liveness |

Probes: `livenessProbe` GET `/healthz` on **8092**, `initialDelaySeconds: 60` (the initial pass
is a full sweep of four mailboxes), `periodSeconds: 30`, **`failureThreshold: 3`** (SPEC D7; not
the Slack watcher's 20 — that one absorbs a Mac mini outage, whereas here the verdict already
carries a 30-minute window, so three failed probes past it is a watcher that has not completed a
pass in 31.5 minutes and a restart is the right answer). `/healthz` is 200 iff a pass COMPLETED
within 30 min AND the lock is held; it deliberately ignores whether any mailbox's IDLE is failing
(that is `imap_idle` rows on `/funnel`, and a restart cannot fix a credential). `startupProbe` is
unnecessary: a standby replica answers 503 `standby` (the lock is judged before pass freshness, so
that is the word an operator sees), and standby only happens during a node drain's overlap, where
the old pod is already terminating.

The watcher prints one startup line — check it after the first roll:
`watch: mode=live horizon=720h reconcile=10m idle_refresh=25m pass_timeout=10m accounts=4 health=:8092`.
`mode=shadow` there means the manifest forgot `CAPTURE_RULES_MODE`.

Env parity check, both lists side by side (only the four `MAIL_*` watch knobs may differ):

```bash
kubectl -n ops get cronjob connector-google -o jsonpath='{range .spec.jobTemplate.spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' | sort > /tmp/cron.env
kubectl -n ops get deploy connector-google-watch -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' | sort > /tmp/watch.env
diff /tmp/cron.env /tmp/watch.env
```

## 3. Rollout order — the Deployment first, verified, then the CronJob's schedule

**Order matters.** Never the reverse: a gap with neither running is a mail outage nobody would
notice for ten minutes.

1. Roll the tag (step 1).
2. Create the Deployment. Wait for `/healthz` 200 (port-forward 8092) and the startup line with
   `accounts=4` and `mode=live`.
3. Wait for **one observed wake**: `kubectl -n ops logs deploy/connector-google-watch | grep
   'watch: wake'` — send a mail to `sspataro@gmail.com` if none arrives on its own; expect
   `watch: wake sspataro@gmail.com normalized=1` within seconds, then a `capture_rules:` line, then
   `pipelined` logging `wake topic=ops/pipeline/captured source=google`.
4. Only after 2 and 3: change `cronjob/connector-google`'s schedule from `*/10 * * * *` to
   `0 */2 * * *`. `concurrencyPolicy: Forbid` stays. **Do not `suspend` it** — Salvador chose the
   2-hourly net over suspension. Its ticks now mostly report `accounts_busy: 0` and no new mail;
   one that lands during a watcher pass reports `accounts_busy` and does nothing, which is correct.

## 4. Post-roll check (the day after)

- The watcher's log shows `watch: wake <mailbox>` lines and a `watch: reconcile` every 10 min;
  `/funnel` shows the google accounts fresh on phase `imap`. No `imap_idle` phase on a healthy
  mailbox is the expected shape (it appears only on failure and recovery).
- The spec's pre-check 0a query, re-run: p50 for INBOX mail collapses from ~5 min (the one clean
  before-picture: `salvador@handsonconnect.org` p50 5m22s / p90 9m35s over 7 days) to seconds.
- `kubectl -n ops logs deploy/connector-google-watch --since=1h | grep -c 'exceeded MAIL_PASS_TIMEOUT'`
  is 0 on an ordinary hour.

## 5. Rollback

Two patches, no image, no data loss (same cursors, same raw rows, same locks):

```bash
kubectl -n ops scale deploy/connector-google-watch --replicas=0
kubectl -n ops patch cronjob connector-google -p '{"spec":{"schedule":"*/10 * * * *"}}'
```

Mail ingestion is back on the ten-minute poll within ten minutes. Rolling the image back is not
needed: the new code runs only under `--watch`.
