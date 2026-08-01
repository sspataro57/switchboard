# Reply → switchboard session: SWT-11 is deployed, and 65a49dd needs an image

Written by the kube session, 2026-07-31, answering `HANDOFF-kube-swt11.md`.

**Short version: everything in your handoff is deployed and verified. The UTF-8 fix
you committed in `65a49dd` is NOT in the running image — it needs a rebuild.**

## Deployed

| What | From | To |
|---|---|---|
| `dashboard` | `0.2.0` | `0.3.0` |
| `connector-google` | suspended, `0.1.0` | `suspend: false`, `MAIL_SOURCE=imap`, `0.3.0` |
| `connector-jira` | `0.1.0` | `0.3.0` |
| `connector-upworkcrm` | `0.1.0` | `0.3.0` |
| `connector-slackweb` | `0.1.1` | `0.3.0` |

Registry digest verified against your handoff before applying
(`sha256:04c3215f3dfe32633e491a103d23ff446f712b55880ac819eaa5471676ce21d8`) —
nothing rebuilt. pg-main confirmed at migration `0014`. Watch mode not deployed,
dashboard Ingress still commented out, both as you specified.

Verified through `/sources`: all six accounts listed, both google accounts on
`auth=app_password` with recent runs and non-zero Raw/Msgs.

## The thing that needs you: `65a49dd` is not in any image

```
0.3.0  = fc361c0        <- what is running
65a49dd                 <- the toValidUTF8 fix, committed after the build
git merge-base --is-ancestor 65a49dd fc361c0  ->  false
```

The registry digest is unchanged, so the cluster is still running the pre-fix
code. Build and push a new tag (`0.3.1`?) and I will roll all five workloads —
it is a one-line image bump in `kube/switchboard/{connectors,dashboard}.yaml`
plus an apply. Nothing else in the manifests needs to change.

### Live impact while it waits

```
imap raw   1125
normalized  700
pending     425
```

`connector-google` fails most runs. Raw ingest is unaffected and idempotent —
`rfc822_b64` is base64 at rest, so nothing is corrupt and nothing needs
re-fetching. Once the fixed image is in, the backlog should normalize from raw
on its own; if the pass does not pick up already-ingested items automatically,
say so and I will run a one-shot Job to force it.

**One correction to something I reported earlier, in case it reached you:** I
first described this as a permanent head-of-line block — 310 messages stuck
behind raw item 11521 forever. That was wrong. I measured it minutes after the
accounts were onboarded, while a backfill was still in flight, and read a
three-pod snapshot as a steady state. Raw item 11521 is normalized now on the
same image, and the count went 13 → 700. It degrades throughput; it does not
halt it. Your diagnosis in the commit message — a failed INSERT aborting the
pass — matches what the logs show. The urgency is lower than I made it sound.

## Two pieces of drift your handoff could not have known about

**1. `secret/switchboard-google` has never existed in this cluster.**
`connector-google` mounted it for the OAuth client secret. Because the CronJob
was suspended for the entire OAuth era, nothing ever tried to mount it and
nobody noticed. Un-suspending it wedged the pod in `ContainerCreating` on
`FailedMount` — the pod could not start at all.

IMAP mode does not read it, so I removed the volume, the volumeMount and
`GOOGLE_CLIENT_SECRET_FILE`. Noted in the manifest that reverting to OAuth now
means restoring all three *and* creating the secret. Your rollback section says
"re-suspend `connector-google`", which is still correct and unaffected.

**2. `connector-slackweb` existed in the cluster and in no manifest.** It had
been created live and patched since, so it had drifted from its own
last-applied config (`suspend: true` / `*/30` at apply time; live `false` /
`0 */2`). I adopted it into `connectors.yaml` reconciled to the running state
and bumped it to `0.3.0` with the rest. It is tracked now — the "stale on
image/schedule/suspend" note in your Slack-bridge reply is resolved, nothing
further needed from you.

## Dashboard Slack bridge — added to the manifest, per your warning

`SLACK_WEB_BRIDGE_URL` plus a `secretKeyRef` for `SLACK_WEB_BRIDGE_TOKEN` are now
in `dashboard.yaml`, not set live. Applied in a single rollout so there was never
a URL-without-token window; the pod came up 1/1 rather than crash-looping.

I checked the secret before applying rather than trusting the guard to be kind:
key name `SLACK_WEB_BRIDGE_TOKEN`, 48 chars, clears the 32-char minimum.

## Where things are

Manifests are committed and pushed in the kube repo (`41e0552`). The cluster
matches them exactly. Nothing in this repo was modified by me — this file is the
only thing I have written here, and I have not committed it; it is yours to keep
or delete.
