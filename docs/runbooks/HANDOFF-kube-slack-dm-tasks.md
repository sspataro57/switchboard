# Handoff to the kube session — Slack DMs are always tasks (SWT-78)

Salvador, 2026-09-23: "there are a bunch of slacks from yesterday that didn't come in as tasks" / "fix it so
DMs skip qwen" / "one task per conversation" / "keep going until it's deployed and backfilled". On 09-22 the
inquiry lane (qwen) dropped 16 of 22 DMs to him as "no reply needed", a verdict nothing reads. Capture now
turns a person's DM straight into its conversation's task. Bug docs: `docs/bugs/slack-messages-not-becoming-tasks*`;
runbook: `docs/runbooks/capture-rules.md`, "Slack DMs are always tasks".

Image: `192.168.50.20:5000/switchboard:0.7.47`
(`sha256:d2e59504a8e351b1760f26626da63de0f2a23798e949e49b74dfafffd65d779c`), built from `main` at `196ca53`.
It includes everything in 0.7.46.

## 1. No migration, no env var, no manifest change beyond the tag

`migrations/` is unchanged (still 0042).

## 2. Roll ONE tag to EVERY workload, the watcher included, in ONE apply

This is the one hard requirement. A capture decision is permanent, and every connector's capture pass decides
every pending message, Slack DMs included. A DM decided by an old binary during a staggered roll goes to qwen
for good. So all 13 move together: the 5 Deployments **including `connector-slackweb-watch`** and the 8
CronJobs. Keep the usual pins.

**About the watcher hold for `switchboard-d2`:** that session has ended (it is gone from ListAgents), and the
reason for the hold is resolved: delivery 58 is `sent`, there are 0 `slack_reply` rows in `sending`, and the
bridge's `send_queue.waiting` is 0 (checked 2026-09-23). The SWT-76 rule still applies at roll time: re-check
`send_queue.waiting == 0` before replacing the watcher pod. If you want Salvador's word before releasing the
hold, ask him; nothing else blocks it.

## 3. Post-roll, done by the switchboard session (tell it when the roll is complete)

- Sweep today with the repro script and `direct-backfill` any DM claimed during the roll window.
- Backfill 2026-09-22's 17 DMs (José ×14, Katie ×3) with
  `opsctl capture-rules direct-backfill --message …`, dry run first.

## 4. Check

- The next DM from a person: its capture decision reads `DM task: …`, and a task appears (or the
  conversation's open task gets a log line and lands in INCOMING). Nothing from that DM shows in the inquiry
  lane.
- Channel messages still get inquiry verdicts, now stamped `inquiry-v3`.

## 5. Rollback

Roll everything back to 0.7.46 together. Tasks already created stay (they are ordinary human tasks). DMs
arriving under the old binary go to qwen again, as before.
