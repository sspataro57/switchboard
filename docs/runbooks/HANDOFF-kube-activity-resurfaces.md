# Handoff to the kube session — activity resurfaces (SWT-72)

A Jira comment, a direct client email or a Slack message that a capture rule files onto an OPEN
task now puts that task in the board's INCOMING section ("new comment / new email / new slack",
with the sender) until Salvador reviews it — the new Requeue verb, or a close. And an inquiry-lane
ask (a client question that needs his reply) always becomes its OWN task now, never a silent log
line on the ticket's task. Spec: `docs/tickets/activity-resurfaces_SPEC.md`.

Image tag and digest are in the message that accompanies this file (built from `main` after the
merge). It supersedes 0.7.38.

## 1. Migration FIRST — this is a rollout BARRIER

`migrations/0039_task_activity_review.sql` adds three nullable columns to `tasks`
(`activity_at`, `activity_by_message_id`, `reviewed_at`). No backfill, no index, idempotent.

**Apply 0039 before rolling any workload.** The new dashboard selects the columns on every
`/tasks` render and the new connectors/promoter call a tool that writes them, so a new image on a
pre-0039 database fails every render and every capture pass. An old image is unaffected by 0039,
so applying first is always safe. The switchboard session applies it from here with the usual
one-shot (`make migrate` against prod) and confirms in the message — check
`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1` says `0039` before step 2.

## 2. Roll ONE tag to every workload, in one apply

No env var or manifest change beyond the tag. Behaviour changes in: every connector CronJob that
runs capture rules (google, jira, slackweb, upworkcrm), `classify-promote` (both lanes),
`orchestratord` (no behaviour change, same binary) and `deployment/dashboard`. Keep the pins
(classify-promote `--lane personal`; pipelined `PIPELINE_STAGES=…`; `MS_OAUTH_CLIENT_ID` on
connector-google).

A mixed fleet is harmless for the marking (an old binary marks nothing) but NOT for the promoter:
an old promote binary still attaches asks as log lines. Do not leave `classify-promote` behind.

As before: check no Send is in flight before replacing the dashboard pod.

## 3. Post-roll check

- `/tasks?refresh=on` renders. Nothing appears in INCOMING at rollout (no backfill); rows arrive with
  the next capture tick that files a message onto an open task.
- A connector log line after its next run carries the new counter:
  `capture_rules: {..., "pr_closed":0,"activity":N}`.
- `classify-promote` (inquiry lane) log: `"related":N` appears once an ask lands on a thread with an
  open task.

## 4. Rollback

Roll the images back to 0.7.38. The columns stay (forward-only); an old binary ignores them — the
board stops showing activity rows and the promoter attaches asks again. Nothing is lost.
