# Handoff to the kube session: SWT-53 chat-on-closed-task

**After this branch merges, apply migration 0034 to prod BEFORE any image built from main runs.** A new capture binary selects `projects.notifier_senders` and writes `capture_decisions.resurface` on every pass. On a db without 0034, every connector's capture pass fails, and every connector CronJob stalls.

The switchboard session builds and pushes the image. The kube session owns the manifests in `kube/switchboard`. Do the steps in order, one at a time. Nothing here adds a workload, an env var, a Service or an MQTT topic.

**Image:** set by the switchboard session at merge time. Use ONE tag for everything below.

## 1. Migration first: 0034

Apply `migrations/0034_chat_on_closed_task.sql` to the `ops` db with the usual one-shot `migrate` Job on the new image. From a workstation, `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate --dir migrations` does the same.

- Before: 0033 must be in `schema_migrations`, and 0034 must not be. 0035 (SWT-54) may land on another branch. It is independent.
- After, all three must hold:
  - `SELECT count(*) FROM information_schema.columns WHERE (table_name='projects' AND column_name='notifier_senders') OR (table_name='capture_decisions' AND column_name='resurface')` returns `2`;
  - `SELECT count(*) FROM pg_constraint WHERE conname='capture_decisions_resurface_is_task_log'` returns `1`;
  - `SELECT count(*) FROM capture_decisions WHERE resurface` returns `0` (no backfill).
- Old images keep running on a 0034 db: they never read the new columns. But every decision an old capture binary writes takes the default `resurface=false`, and that is permanent (step 3). So move straight on to steps 2 and 3.

## 2. Seed the notifier list, after 0034 and BEFORE the images

Values come from SPEC Verification 0b, read-only on prod 2026-09-14. These are every notifier identity that 0a showed landing on collaboratory's closed bucket tasks in the last 30 days. **Owner decision 2026-09-14: keep GitHub notification mail silent.** `notifications@github.com` stays in the list. The runbook's seed SQL (`docs/runbooks/capture-rules.md`, "Closed-task chats resurface") is this exact statement.

```sql
UPDATE projects SET notifier_senders = ARRAY['Jira','jira@treetopllc.jira.com',
  'notifications@github.com','noreply@github.com','no-reply@github.com','no-reply@builds.circleci.com']
 WHERE slug = 'collaboratory';
SELECT slug, notifier_senders FROM projects WHERE slug = 'collaboratory';
```

Old binaries ignore the column, so seeding first means the first new capture pass already excludes the Jira app (T3 avoided). Matching is exact, never substring: `Jira` is the Slack app's display name, and the mail entries are ADDRESSES.

What the GitHub entry costs: `notifications@github.com` also carries HUMAN PR comments (for example `joseg-avviato <notifications@github.com>`, 173 in 30 days onto buckets 56/57). With the entry, those log silently, as today. GitHub PR tasks are a separate ticket (SWT-54).

## 3. Every capture writer and every inquiry reader: ONE tag, ONE apply

Roll the whole table in ONE `kubectl apply`, right after step 2. Do not stagger it.

| workload | kind | change |
|---|---|---|
| connector-google, connector-jira, connector-slackweb, connector-upworkcrm | CronJob (stays) | image bump (every one runs capture; google's IMAP watch loop runs capture too) |
| connector-gcal, classify-* | CronJob (stays) | image bump (same tag, so no binary lags) |
| pipelined | Deployment (stays) | image bump (its `inquiry` and `inquiry_promote` stages read `resurface`) |
| orchestratord, dashboard | Deployment | image bump (no behaviour change here; keeps one tag) |

**Why one apply: an old-image capture pass records `resurface=false` for good.** A capture binary built before this branch writes the column's default, `false`, for every message it decides. The live decision is unique per message (`ON CONFLICT (message_id) WHERE mode = 'live' DO NOTHING`), and a live pass never re-decides a message. So a human message that an old writer logs onto a closed task after 0034 stays logged but never resurfaces. A shadow re-point cannot recover it either: the resurface path reads only live decisions (SPEC CC5b).

**Recorded residual.** Messages captured by an old writer during the roll window (minutes, from 0034 until the last old CronJob pod finishes) are logged but never resurfaced. That is accepted. One apply keeps the window to the length of the roll.

## 4. Local binaries (switchboard session, not kube)

- **Rebuild before any hand-run capture.** Run `go install ./cmd/opsctl` from `main` wherever `opsctl capture-rules run` or `opsctl capture gate` is run by hand (this workstation and 192.168.50.30). Do it BEFORE the next hand-run pass after 0034. An old opsctl on a 0034 db still runs, but every message it decides live is recorded `resurface=false` for good (step 3), so never run a capture pass from an old binary.

## Smoke (SPEC Verification 4, read-only, within 24h)

```sql
SELECT cd.id, cd.created_at, nm.channel, nm.sender, cd.task_id, cd.resurface, cd.reason
  FROM capture_decisions cd JOIN normalized_messages nm ON nm.id = cd.message_id
 WHERE cd.action = 'task_log' AND cd.created_at > now() - interval '24 hours'
 ORDER BY cd.id DESC;
```

1. Every row from a notifier-list sender has `resurface=false`, and its reason says `the sender is on the project's notifier list`.
2. Every row with an empty or whitespace sender has `resurface=false`, and its reason says `the message has no sender identity`.
3. A human row onto a closed task has `resurface=true`.
4. Connector logs show `"resurfaced":N` on every `capture_rules:` line, and `"resurfaced":0` on every `capture_gate:` line.
5. For a `resurface=true` message, `classify promote --lane inquiry --dry-run` lists it after its 1h grace, either as a would-create (`status=holding`) or gated with a reason.

**Expect volume.** 0a (30 days, prod) shows many human Slack and Jira-mail senders on the buckets: Jose Garcia 140, Kristin Medlin 125, esteban 40, asunda45 7, and others. It also shows 741 historical Slack rows with an EMPTY sender, which are Jira-app continuation messages. **Those 741 never resurface.** They were decided before 0034, and a decided message is never re-decided. New blank-sender messages never resurface either: capture fails closed on a blank sender (SPEC CC4b), and logs them silently. Since the Slack connector fix at 17:00Z on 2026-09-14, no new inbound Slack message has had an empty sender (prod, read-only, 2026-09-14: 7 new messages, all named).

**Shadow rows and resurfacing** (SPEC CC5b). The resurface path reads only live decisions: a shadow row never adds a message, and a newer shadow row of any other action never removes one. The one accepted exception (owner decision 2026-09-14, the re-point contract): a NEWER shadow `attributed` row takes precedence, so the message is admitted as an attributed message under that row's project, with no `logged_on_closed_task` line, or dropped if that project is not armed.

**Rollback.** Revert pipelined's image, and the lanes stop reading `resurface`. The schema stays (forward-only). Emptying `notifier_senders` is not a rollback, because it only widens resurfacing.
