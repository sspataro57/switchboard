# Handoff to the kube session — a reply on a closed task reopens it into INCOMING (swb 650, SWT-88)

Salvador, 2026-09-25: "those are supposed to be on incoming". Lyle Deitch's replies on closed foundry tasks were
logged and handed to an inquiry lane foundry never armed, so nothing read them. On a project without an armed
inquiry lane and without the ticket gate, a person's message on a closed task now reopens it into INCOMING.
Collaboratory (armed) is unchanged.

Image: `192.168.50.20:5000/switchboard:0.7.54`
(`sha256:731c6a0ad5c0988fe80c96f58612ccd4b59f608b81096d8ad9298588f4bf7591`), built from `main` at `e119068`.
It includes everything in 0.7.53.

## 1. No migration, no env var, no manifest change beyond the tag

## 2. Roll ONE tag to ALL 13 workloads in ONE apply

Capture runs in every connector, so all 13 move together. Keep the pins. Before replacing the watcher pod, run
the SWT-76 check: bridge `send_queue.waiting == 0`.

## 3. Check

The next person's email on a closed foundry or ahs task gets a live capture decision whose reason contains
`swb 650`, and the task is `ready` with `activity_by_message_id` = that message.

## 4. Rollback

Roll all 13 back to 0.7.53 together. Nothing is written that an old binary can't read.
