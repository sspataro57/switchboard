# Handoff to the kube session — an Upwork client's messages become tasks (swb #610)

Salvador, 2026-09-24: "the townai project they only message me using upwork … those messages landing there
should create tasks". An Upwork message that a capture rule attributed now takes the SWT-78 DM
conversation-task path: one open task per room, no classifier. Before this, the messages were logged onto
closed task #80 and never reached the board. Runbook: `docs/runbooks/capture-rules.md`, "Slack DMs are
always tasks", Upwork paragraph.

Image: `192.168.50.20:5000/switchboard:0.7.52`
(`sha256:88c723f2f9d2476fe17884431f6e76c83e18fd19866a008cef3c3cb15f9b710e`), built from `main` at `1eb1c07`.
It includes everything in 0.7.51.

## 1. No migration, no env var, no manifest change beyond the tag

## 2. Roll ONE tag to ALL 13 workloads in ONE apply

Capture runs in every connector, so all 13 move together. Keep the pins. Before you replace the watcher pod,
run the usual SWT-76 check: bridge `send_queue.waiting == 0`.

## 3. Check

The next Upwork message from a mapped client whose ref task is closed gets a live capture decision with
action `task` or `task_log` and a reason containing `DM task: … an Upwork conversation`. It does NOT get a
`task_log` onto the closed task with `resurface=true`. Already backfilled by hand: task 611 (town-ai, Erica
Rapa).

## 4. Rollback

Roll all 13 back to 0.7.51 together. Nothing is written that an old binary can't read.
