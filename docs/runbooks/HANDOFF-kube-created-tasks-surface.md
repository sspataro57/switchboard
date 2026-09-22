# Handoff to the kube session — rule-created tasks land in INCOMING (swb #491, SWT-72 follow-up)

A task a capture rule creates from a person's first email/comment/Slack message (Lyle's two emails
this morning became #487 and #488) now lands in the board's INCOMING section with the sender, instead
of silently in QUEUE. One `if` in capture's create path; no schema change (0039 already holds the
columns).

Image tag and digest are in the message that accompanies this file. Supersedes 0.7.39.

## 1. No migration, no env var, no manifest change beyond the tag

## 2. Roll the tag

Behaviour changes only in the capture writers — the four connector CronJobs (google, jira, slackweb,
upworkcrm). Roll the same tag to all 11 workloads as usual, keeping the pins. Check no Send is in
flight before replacing the dashboard pod.

## 3. Post-roll check

The next first-message capture (a connector log line with `"tasks_created":1`) also shows
`"activity":1`, and the new task appears in INCOMING on `/tasks`.

## 4. Rollback

Tag back to 0.7.39. Tasks created under 0.7.40 keep their mark; a Requeue clears it.
