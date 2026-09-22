# Handoff to the kube session — comms inbox (SWT-74)

A person's comment, direct email or Slack message that a capture rule files onto an OPEN task can
now become its OWN task in INCOMING — a "comm" to answer, route (`Attach`) or dismiss — instead of
a log line surfacing the ticket task. Per rule, opt-in, as data (`capture_rules.comm_task`). Also
two verbs for Claude Code sessions: `swb match <id>` (which task does this belong to, by capture's
own rules) and `swb attach <id> <target>` (route the comm onto a task and close it).
Spec: `docs/tickets/comms-inbox_SPEC.md`.

Image tag and digest are in the message that accompanies this file (built from `main` after the
merge). It supersedes 0.7.41.

## 1. Migration FIRST — a rollout BARRIER

`migrations/0040_capture_comm_tasks.sql` adds `capture_rules.comm_task` (default false) and
`capture_decisions.comm_task_id`, with two CHECKs. **Every capture pass built from this image
selects `capture_rules.comm_task`**, so a new image on a pre-0040 database fails capture for every
connector (the 0034/0035 precedent). An old image ignores both columns, so applying first is
always safe. The switchboard session applies it from here and confirms in the message — check
`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1` reads `0041` (0041 is
already applied; 0040 lands beside it) and
`SELECT count(*) FROM information_schema.columns WHERE column_name IN ('comm_task','comm_task_id')`
reads 2 before step 2.

## 2. Roll ONE tag to every workload, in one apply

No env var or manifest change beyond the tag. Behaviour changes in every capture writer (the four
connector CronJobs plus the Slack watcher Deployment), the dashboard (Attach form + route) and the
MCP surface. Keep the pins. Check no Send is in flight before replacing the dashboard pod.

**Nothing changes in behaviour at rollout**: with no rule armed, the funnel is byte-identical to
0.7.41. Arming is the switchboard session's step 3, after the roll.

## 3. Post-roll check

- `/tasks?refresh=on` renders; every row's actions popup has an `Attach` form.
- The next connector line ends `"activity":N,"comm_tasks":0`.
- After the switchboard session arms one rule: the next person's message on that rule shows in
  INCOMING as its own row (`new email` / `new comment` / `new slack` + sender), and the ticket task
  stays in QUEUE with two log lines; the connector line shows `"comm_tasks":1`.

## 4. Rollback

Two levels, mildest first. (a) `UPDATE capture_rules SET comm_task = false` — one statement, no
deploy: every rule back to SWT-72's behaviour on the next message. (b) Roll the images back to
0.7.41: the columns stay (forward-only) and are ignored; comm tasks already created stay as
ordinary human tasks.
