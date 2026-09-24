# Handoff to the kube session — a person's Jira comment holds its open task (SWT-82)

Salvador, 2026-09-24: "make QA comments reopen the task". A person's Jira comment that lands while its
task is still open now holds the task against the ticket-status reconciler. Before, the reconciler
closed it two seconds later when the ticket was In QA. Bug doc: `docs/bugs/qa-comment-holds-task.md`.

Image: `192.168.50.20:5000/switchboard:0.7.51`
(`sha256:3a31464457b8929c70e6b9d747f9056f261aa994108d8654abc430fc5339cb11`), built from `main` at `0e3f6b0`.
It includes everything in 0.7.50.

## 1. No migration, no env var, no manifest change beyond the tag

## 2. Roll ONE tag to ALL 13 workloads in ONE apply

Capture runs in every connector, so all 13 move together. Keep the pins. Before replacing the watcher
pod, run the usual SWT-76 check: bridge `send_queue.waiting == 0`. Every `capture_rules:` and
`capture_gate:` line gains `"surfaced_open":N`.

## 3. Check

The next person's comment on a collaboratory ticket in TT-In QA leaves the task open in INCOMING, and
its `ticket_status_syncs.last_action` becomes `resurfaced`, not `closed`.

## 4. Rollback

Roll all 13 back to 0.7.50 together. Nothing is written that an old binary can't read.
