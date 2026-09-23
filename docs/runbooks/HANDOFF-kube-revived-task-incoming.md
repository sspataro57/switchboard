# Handoff to the kube session — a task a comment reopens lands in INCOMING (SWT-80)

Salvador, 2026-09-23: "so there is comment in jira from katie and the task didn't reopen". It had reopened, but
into QUEUE: capture and promote marked the comment's activity before the reopen, and the mark skips a closed
task. The mark now runs after the reopen. Bug docs: `docs/bugs/revived-task-not-in-incoming*`.

Image: `192.168.50.20:5000/switchboard:0.7.49`
(`sha256:e466aadfda394a53f2e6b82bc565c8133a815e29c9498f6c241c8ba0012c905c`), built from `main` at `2971e0a`.
It includes everything in 0.7.48.

## 1. No migration, no env var, no manifest change beyond the tag

## 2. Roll ONE tag to ALL 13 workloads in ONE apply

As for SWT-78/79: capture runs in every connector, and the promote step runs in `pipelined`. Keep the pins,
and the usual SWT-76 check (bridge `send_queue.waiting == 0`) before replacing the watcher pod.

## 3. Check

The next comment that revives or reopens a closed task: the task comes back AND is in INCOMING
(`tasks.activity_by_message_id` = that comment). Already fixed by hand: #381 (Katie, WEB-10362).

## 4. Rollback

Roll all 13 back to 0.7.48 together. Nothing is written that an old binary can't read.
