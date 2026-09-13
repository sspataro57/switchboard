# Handoff to the kube session: SWT-45 jira-activity-revive

**After this branch merges, migration 0030 must be applied to prod BEFORE any image built from main runs. A new capture binary selects `capture_rules.revive` on every pass, and every new close writes `tasks.closed_at`. On a db without 0030, every connector's capture pass fails and every close fails.**

The switchboard session builds and pushes the image. The kube session owns the manifests in `kube/switchboard`. Do the steps in order, one at a time. Nothing here adds a workload, an env var or a Service.

**Image:** `192.168.50.20:5000/switchboard:0.7.15` (`sha256:8f596fcaa024c9a02f96990dd6e7b37e1d1996f0157728fc2cef7d553de9f5f9`, main 42ef053; handed off 2026-09-13). It also carries SWT-40 Part C, so keep Part C's settings as they are: classify-promote pinned to `--lane personal`, and pipelined's `PIPELINE_STAGES` unchanged until that handoff's step 4. Use ONE tag for everything below.

## 1. Migration first

Apply `migrations/0030_jira_activity_revive.sql` to the `ops` db with the usual one-shot `migrate` Job on the new image. From a workstation, `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate --dir migrations` does the same.

- Before: 0028 and 0029 must be in `schema_migrations`, and 0030 must not be. If 0028/0029 are not applied, stop: 0030 assumes them. 0031 (SWT-40 Part C) may already be applied; it merged first and touches only `projects`, so `max(version)` can read `0031` here. That is expected.
- After: 0030 is present. Also check `SELECT count(*) FROM pg_constraint WHERE conrelid='ticket_status_syncs'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%last_action%'` returns `1`. The migration's own `DO $$` self-check already raises otherwise.
- Old images are unaffected by 0030. They never read the new columns. Their closes leave `closed_at` NULL, and the revive guard falls back to `updated_at` for those.

## 2. Connectors and pipelined: one tag, together

| workload | kind | change |
|---|---|---|
| connector-google, connector-jira, connector-slackweb, connector-upworkcrm | CronJob (stays) | image bump |
| connector-gcal, classify-* | CronJob (stays) | image bump (same tag, so no binary lags) |
| pipelined | Deployment (stays) | image bump |

Roll these in ONE change. A new capture pass that revives a task, running beside an old connector-jira reconciler that ignores surfacing, re-closes that task every jira tick: one flip per message until the images match. With no revive rule seeded yet (step 5), nothing revives, so this window is quiet. Keep it short anyway.

## 3. Then orchestratord and the dashboard

| workload | kind | change |
|---|---|---|
| orchestratord | Deployment | image bump |
| dashboard | Deployment | image bump |

Both call `task_close`. Until they roll, their closes carry `closed_at` NULL. The fallback covers that, so there is no ordering hazard beyond "after step 1".

## 4. Local binaries (switchboard session, not kube)

- `go install ./cmd/ops-mcp-user` on this workstation AND on 192.168.50.30, from `main` (`docs/runbooks/ops-mcp-user-scope.md`). Open new Claude sessions afterwards; an open session keeps the old binary.
- Re-run any hand-held `opsctl` from `main`.

## 5. Seeding waits for the last workload

Seed revive rules only after EVERY workload above reports the new tag (`kubectl -n ops get cronjob,deploy -o wide`). An old capture binary does not read `revive`/`addressed`. It would treat the J2 rule as a plain priority-92 rule: it would create unsurfaced per-ticket tasks and log on closed tasks without reviving them.

Two more gates before J2, both owned by the switchboard session:

- **The owner turns off Jira's "Notify me about my own changes"** (Treetop Jira, personal settings → Email notifications). With it on, Jira mails him "Anonymous (JIRA)" notifications about his own changes. The own-action guard catches only his own COMMENTS (26 of 208 on prod). His field and status edits (the other 182) would revive or create. The setting is the real protection.
- **SPEC Verification 0c passes** after the setting change. 0c is an UNCORRELATED count of "Anonymous (JIRA)" Treetop mail received after the moment the setting went off, observed over at least a full working day. 0 new → seed J2. Any still arriving are not his own changes (likely automation, such as GitHub-driven transitions): STOP seeding and bring them to the owner, who decides an exclusion in the rule's data (sender or pattern), not in Go. 0c's correlation query is informational only.

Seed **J2 only** (SPEC J2, `docs/runbooks/capture-rules.md` "Activity rules (SWT-45)"). **J4 is not seeded**: prod has 0 Avviato addressed-subject shapes.

## Smoke (SPEC Verification 7-8)

1. `opsctl ticket-status sync --dry-run` matches the pre-change baseline line for line (nothing is surfaced before seeding).
2. The task-85 hand-reopen smoke (SPEC step 7): `resurfaced=1` once, then `0`.
3. After seeding J2, connector logs show `"revived"` / `"surfaced_created"` / `"deferred"` / `"blind"` counters. A `capture rules: message N deferred: … own-action guard deferred` line means an email arrived before connector-jira polled the ticket. It clears on the next jira tick, or proceeds after 30 min with the reason saying the guard ran BLIND (counted in `"blind"`). A steady nonzero `"blind"` means connector-jira is not syncing; look there.

## Config warning: CAPTURE_RULES_SINCE

Leave `CAPTURE_RULES_SINCE` unset on the connector CronJobs (the live default is 720h). This image refuses a LIVE horizon below **2h** (`MinLiveRulesHorizon`): a positive value such as `30m` makes every live capture pass return an error, and the CronJob run fails. A google `--watch` loop, if one is ever deployed, only logs that error and prints a zero counter line on every wake, so it looks alive while capturing nothing. A value that doesn't parse, or isn't positive, still falls back silently to 720h.
