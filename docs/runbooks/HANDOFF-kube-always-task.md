# Handoff to the kube session: always_task capture rules (SWT-93)

Salvador, 2026-09-25: mail from Pines Property Management "should always pop in incoming as personal". A keyless
capture rule can now carry `always_task`: every non-Slack message it attributes becomes a task (one per thread) in
INCOMING, before any classifier. SPEC: `docs/tickets/always-task_SPEC.md`.

Image: `192.168.50.20:5000/switchboard:0.7.58`
(`sha256:8fb5856b15165ed3b0743fbcc55a770656cca40e37a4872d6f62bd2b5fb12af0`), built from `main` at `1df98f1`.
It includes everything in 0.7.57.

## 0. Already done

**Migration 0045 is applied to production** (switchboard session, 2026-09-25 22:13Z). It adds
`capture_rules.always_task` (default false) and a keyless CHECK. The running 0.7.57 ignores the column.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Every capture writer (the connector watchers and the connector CronJobs) reads the new column in loadRules, so every
workload needs the same tag. Keep the pins, and do the SWT-76 check (bridge `send_queue.waiting == 0`) before
replacing the watcher pod. No env var, port or probe change.

## 2. After the roll (switchboard session does this, not kube)

- Add the Pines rule with always_task, then disable rule 19.
- Backfill message 590311.

## 3. Rollback

Roll all 13 back to 0.7.57. 0045 can stay, because an old image ignores the column. If the Pines rule was already
added, it just stays attribution-only under 0.7.57.
